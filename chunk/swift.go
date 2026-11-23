package chunk

import (
	"bytes"
	"slices"
	"strings"
)

// Swift files that fail to parse can take tree-sitter tens of seconds and
// hundreds of megabytes each: when the first parse of a Swift file has
// errors, gotreesitter tries to rescue it by reparsing the whole file, and
// each error-carrying top-level declaration on its own, through its retry
// ladder, and the cost of each reparse grows faster than the file. A 60 KB
// SwiftUI file whose one 13-line extension doesn't parse took 20 s, while
// the same file's declarations parsed one at a time took 0.4 s. Files that
// parse never come near that: the slowest seen took 0.3 s.
//
// So before parsing a Swift file whole, swiftUnits cuts it into units at
// declaration boundaries (top-level declarations, and the members of big
// types, each kept inside its enclosing headers so it reads as it does in
// the file) and parseEntry parses those first, small enough that even a
// failing one is quick. When one fails the file is sure to fail too: the
// unit is a declaration as the file holds it, and gotreesitter's own rescue
// reparses failing declarations on their own, so it would fail that unit
// the same way. The file then goes straight to the plain splitter, which is
// where its whole-file parse would have sent it, only slower. When every
// unit parses, the file is parsed whole exactly as before, so a file that
// parses gets the same tree, and the same chunks, as it always did.
//
// The decision rests only on the file's bytes, never on the clock.

// swiftOpenBytes is the size above which a declaration with a body is
// opened up and its members become units of their own. Smaller ones are
// parsed as they are: even failing they take tens of milliseconds.
const swiftOpenBytes = 2 << 10

// swiftLines is what scanSwift learns about each line of a Swift file.
type swiftLines struct {
	src    []byte
	starts []int   // byte offset of each line, plus len(src) as a sentinel
	depth  []int32 // brackets ({, ( and [) open at the line's start
	ifs    []int32 // #if blocks open at the line's start
	clean  []bool  // the line starts in code, not in a comment or string
	last   []int   // offset of the line's last code byte, or -1 if it has none
	braces []bracePair
}

// bracePair is a { and its }, and the bracket depth outside them.
type bracePair struct {
	open, close int
	depth       int32
}

// swiftUnit is the text pre + src[lo:hi] + suf.
type swiftUnit struct {
	pre, suf []byte
	lo, hi   int
}

func (u swiftUnit) size() int { return len(u.pre) + u.hi - u.lo + len(u.suf) }

func (u swiftUnit) text(src []byte) []byte {
	out := make([]byte, 0, u.size())
	out = append(out, u.pre...)
	out = append(out, src[u.lo:u.hi]...)
	return append(out, u.suf...)
}

// scanSwift reads src's comments, strings and brackets. It fails on
// anything it can't follow (unbalanced brackets, an unterminated string or
// comment, an #endif without its #if), and the file is then parsed whole.
func scanSwift(src []byte) (*swiftLines, bool) {
	var starts []int
	for i := 0; i < len(src); {
		starts = append(starts, i)
		j := bytes.IndexByte(src[i:], '\n')
		if j < 0 {
			break
		}
		i += j + 1
	}
	starts = append(starts, len(src))
	n := len(starts) - 1
	sl := &swiftLines{
		src:    src,
		starts: starts,
		depth:  make([]int32, n),
		ifs:    make([]int32, n),
		clean:  make([]bool, n),
		last:   make([]int, n),
	}

	const (
		code = iota
		blockComment
		str
		regex
	)
	type strState struct {
		hashes int
		multi  bool
		depth  int // bracket depth where the interpolation began
	}
	var (
		mode     = code
		nest     int        // block comment nesting
		cur      strState   // the string or regex being read
		interp   []strState // strings waiting for an interpolation to close
		brackets []int      // open brackets: an index into braces, or -1 for ( and [
		kinds    []byte     // their closing byte
		ifs      int32
		line     = -1
		lineCode bool // the current line has code before i
	)
	hashesAt := func(i, h int) bool {
		if i+h > len(src) {
			return false
		}
		for k := i; k < i+h; k++ {
			if src[k] != '#' {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(src); {
		for line+1 < n && starts[line+1] <= i {
			line++
			sl.depth[line] = int32(len(brackets))
			sl.ifs[line] = ifs
			sl.clean[line] = mode == code && len(interp) == 0
			sl.last[line] = -1
			lineCode = false
		}
		c := src[i]
		switch mode {
		case blockComment:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				nest++
				i += 2
			case c == '*' && i+1 < len(src) && src[i+1] == '/':
				nest--
				i += 2
				if nest == 0 {
					mode = code
				}
			default:
				i++
			}
			continue
		case str:
			switch {
			case c == '\\' && hashesAt(i+1, cur.hashes):
				j := i + 1 + cur.hashes
				if j < len(src) && src[j] == '(' {
					cur.depth = len(brackets)
					interp = append(interp, cur)
					mode = code
					i = j + 1
					continue
				}
				i = j
				if j < len(src) && src[j] != '\n' {
					i++ // the escaped byte
				}
			case c == '"' && !cur.multi && hashesAt(i+1, cur.hashes):
				mode = code
				i += 1 + cur.hashes
			case c == '"' && cur.multi && bytes.HasPrefix(src[i:], []byte(`"""`)) && hashesAt(i+3, cur.hashes):
				mode = code
				i += 3 + cur.hashes
			case c == '\n' && !cur.multi:
				return nil, false
			default:
				i++
			}
			if mode == code {
				sl.last[line] = i - 1
				lineCode = true
			}
			continue
		case regex:
			switch {
			case c == '/' && hashesAt(i+1, cur.hashes):
				mode = code
				i += 1 + cur.hashes
				sl.last[line] = i - 1
				lineCode = true
			case c == '\\' && i+1 < len(src) && src[i+1] != '\n':
				i += 2
			default:
				i++
			}
			continue
		}

		// Code.
		switch c {
		case ' ', '\t', '\r', '\n', '\f', '\v':
			i++
			continue
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				if j := bytes.IndexByte(src[i:], '\n'); j >= 0 {
					i += j
				} else {
					i = len(src)
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				mode, nest = blockComment, 1
				i += 2
				continue
			}
		case '"', '#':
			h := 0
			for i+h < len(src) && src[i+h] == '#' {
				h++
			}
			j := i + h
			switch {
			case j < len(src) && src[j] == '"':
				mode = str
				cur = strState{hashes: h}
				if bytes.HasPrefix(src[j:], []byte(`"""`)) {
					cur.multi = true
					i = j + 3
				} else {
					i = j + 1
				}
				lineCode = true
				continue
			case h > 0 && j < len(src) && src[j] == '/':
				mode = regex
				cur = strState{hashes: h}
				i = j + 1
				lineCode = true
				continue
			case h == 1 && !lineCode:
				// A compiler directive.
				w := j
				for w < len(src) && isWordByte(src[w]) {
					w++
				}
				switch string(src[j:w]) {
				case "if":
					ifs++
				case "endif":
					if ifs--; ifs < 0 {
						return nil, false
					}
				}
			}
		case '{', '(', '[':
			idx := -1
			closer := byte(')')
			switch c {
			case '{':
				idx = len(sl.braces)
				sl.braces = append(sl.braces, bracePair{open: i, close: -1, depth: int32(len(brackets))})
				closer = '}'
			case '[':
				closer = ']'
			}
			brackets = append(brackets, idx)
			kinds = append(kinds, closer)
		case '}', ')', ']':
			if c == ')' && len(interp) > 0 && interp[len(interp)-1].depth == len(brackets) {
				cur = interp[len(interp)-1]
				interp = interp[:len(interp)-1]
				mode = str
				i++
				continue
			}
			k := len(brackets) - 1
			if k < 0 || kinds[k] != c {
				return nil, false
			}
			if idx := brackets[k]; idx >= 0 {
				sl.braces[idx].close = i
			}
			brackets, kinds = brackets[:k], kinds[:k]
		}
		sl.last[line] = i
		lineCode = true
		i++
	}
	if mode != code || len(interp) > 0 || len(brackets) > 0 || ifs != 0 {
		return nil, false
	}
	return sl, true
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// swiftDeclarators name what a declaration declares, and swiftModifiers
// may come before them. A unit starts at a line beginning with either (or
// a comment, an attribute or an #if); "case" begins an enum case, or a
// case of a switch.
var (
	swiftDeclarators = wordSet("import struct class enum protocol extension actor func init deinit " +
		"subscript typealias associatedtype let var case operator precedencegroup macro")
	swiftModifiers = wordSet("public private fileprivate internal open package final static override " +
		"mutating nonmutating nonisolated isolated indirect convenience required lazy weak unowned " +
		"dynamic optional prefix postfix infix consuming borrowing distributed")
)

func wordSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// startsUnit reports whether a unit may start at line r, inside brackets
// at depth d and #if blocks at depth ifs: r begins a declaration (or its
// comment or attribute, or an #if around it) and the code before it is
// finished, so cutting there leaves complete declarations on both sides.
func (sl *swiftLines) startsUnit(r int, d, ifs int32) bool {
	if !sl.clean[r] || sl.depth[r] != d || sl.ifs[r] != ifs {
		return false
	}
	line := sl.src[sl.starts[r]:sl.starts[r+1]]
	t := bytes.TrimLeft(line, " \t")
	switch {
	case bytes.HasPrefix(t, []byte("//")), bytes.HasPrefix(t, []byte("/*")),
		bytes.HasPrefix(t, []byte("@")), bytes.HasPrefix(t, []byte("#if")):
	default:
		w := 0
		for w < len(t) && isWordByte(t[w]) {
			w++
		}
		if word := string(t[:w]); !swiftDeclarators[word] && !swiftModifiers[word] {
			return false
		}
	}
	// The line before must be blank, close a declaration or an #if block,
	// and the last code before r must not leave an expression open.
	prev := bytes.TrimSpace(sl.src[sl.starts[r-1]:sl.starts[r]])
	if len(prev) > 0 && prev[0] != '}' && !bytes.HasPrefix(prev, []byte("#endif")) {
		return false
	}
	for p := r - 1; p >= 0; p-- {
		if sl.last[p] >= 0 {
			switch sl.src[sl.last[p]] {
			case ',', '=', ':', '.', '&', '|', '+', '-', '*', '/', '%', '^', '~', '<', '\\':
				return false
			}
			break
		}
	}
	return true
}

// units cuts rows a..b-1, which sit inside brackets at depth d and #if
// blocks at depth ifs, into units wrapped in pre and suf.
func (sl *swiftLines) units(a, b int, d, ifs int32, pre, suf []byte, out []swiftUnit) []swiftUnit {
	start := a
	for r := a + 1; r <= b; r++ {
		if r < b && !sl.startsUnit(r, d, ifs) {
			continue
		}
		out = sl.unit(start, r, d, pre, suf, out)
		start = r
	}
	return out
}

// unit adds rows a..b-1 as one unit or, when they're one big declaration
// with a body, the units of its members.
func (sl *swiftLines) unit(a, b int, d int32, pre, suf []byte, out []swiftUnit) []swiftUnit {
	lo, hi := sl.starts[a], sl.starts[b]
	whole := swiftUnit{pre: pre, suf: suf, lo: lo, hi: hi}
	if hi-lo <= swiftOpenBytes {
		return append(out, whole)
	}
	// The first { at depth d must open on a line it ends and close on a
	// line it starts, with nothing but comments and blank lines after it.
	k, _ := slices.BinarySearchFunc(sl.braces, lo, func(p bracePair, lo int) int { return p.open - lo })
	for k < len(sl.braces) && sl.braces[k].open < hi && sl.braces[k].depth != d {
		k++
	}
	if k == len(sl.braces) || sl.braces[k].open >= hi {
		return append(out, whole)
	}
	br := sl.braces[k]
	ro, rc := sl.row(br.open), sl.row(br.close)
	if sl.last[ro] != br.open || sl.last[rc] != br.close || rc <= ro+1 || rc >= b {
		return append(out, whole)
	}
	if len(bytes.TrimLeft(sl.src[sl.starts[rc]:br.close], " \t")) > 0 {
		return append(out, whole)
	}
	for r := rc + 1; r < b; r++ {
		if sl.last[r] >= 0 {
			return append(out, whole)
		}
	}
	if !sl.declaresType(a, br.open) {
		return append(out, whole)
	}
	inner := slices.Concat(pre, sl.src[lo:sl.starts[ro+1]])
	outer := slices.Concat(sl.src[sl.starts[rc]:hi], suf)
	return sl.units(ro+1, rc, d+1, sl.ifs[ro+1], inner, outer, out)
}

// swiftTypeWords declare a type, whose members are declarations. Only
// types are opened up: a function's statements read differently on their
// own. An if condition ending in a name, as in `if a == b {`, parses as a
// trailing closure, and gotreesitter's rescue for that reparses the whole
// source, so whether it succeeds depends on what's around the statement.
var swiftTypeWords = map[string]bool{"struct": true, "class": true, "enum": true, "extension": true, "protocol": true, "actor": true}

// declaresType reports whether the code from line a up to byte end, a
// declaration's header, declares a type: its first word that names what's
// declared (after attributes, modifiers and comments) is a type word.
func (sl *swiftLines) declaresType(a, end int) bool {
	for r := a; sl.starts[r] < end; r++ {
		if !sl.clean[r] || sl.last[r] < 0 {
			continue // a comment or blank line
		}
		line := sl.src[sl.starts[r]:min(sl.starts[r+1], end)]
		for i := 0; i < len(line); {
			if line[i] == '/' && i+1 < len(line) && (line[i+1] == '/' || line[i+1] == '*') {
				break
			}
			if line[i] == '@' { // an attribute: skip its name and arguments
				i++
				for i < len(line) && (isWordByte(line[i]) || line[i] == '.') {
					i++
				}
				if i < len(line) && line[i] == '(' {
					j := bytes.IndexByte(line[i:], ')')
					if j < 0 {
						return false
					}
					i += j + 1
				}
				continue
			}
			if !isWordByte(line[i]) {
				i++
				continue
			}
			w := i
			for w < len(line) && isWordByte(line[w]) {
				w++
			}
			word := string(line[i:w])
			i = w
			switch {
			case word == "class":
				// A type, or a modifier as in "class func".
				rest := bytes.TrimLeft(line[i:], " \t")
				for _, kw := range []string{"func", "var", "let", "subscript", "init"} {
					if bytes.HasPrefix(rest, []byte(kw)) && (len(rest) == len(kw) || !isWordByte(rest[len(kw)])) {
						return false
					}
				}
				return true
			case swiftTypeWords[word]:
				return true
			case swiftModifiers[word]:
				continue
			default:
				return false
			}
		}
	}
	return false
}

// row is the line holding byte i.
func (sl *swiftLines) row(i int) int {
	r, found := slices.BinarySearch(sl.starts, i)
	if !found {
		r--
	}
	return r
}

// swiftUnits cuts a Swift file into units to parse before parsing it
// whole, or returns nil when there's nothing to gain: the file is one unit,
// or scanSwift can't follow it.
func swiftUnits(src []byte) ([]swiftUnit, *swiftLines) {
	sl, ok := scanSwift(src)
	if !ok {
		return nil, nil
	}
	units := sl.units(0, len(sl.starts)-1, 0, 0, nil, nil, nil)
	if len(units) < 2 {
		return nil, nil
	}
	// Smallest first: a failing unit costs more the bigger it is, and the
	// first failure settles it.
	slices.SortStableFunc(units, func(a, b swiftUnit) int { return a.size() - b.size() })
	return units, sl
}
