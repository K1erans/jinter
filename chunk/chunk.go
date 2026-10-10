// Package chunk splits source files into function- or block-sized pieces,
// each sent to Jev as one state.
package chunk

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

type Chunk struct {
	Start, End int    // 1-based, inclusive
	Signature  string // header of the first definition, e.g. "func Retry(n int) error"
	Code       string
	Offset     int // Code's byte offset in the source
}

type Options struct {
	MinLines int // pieces shorter than this merge with their neighbours
	MaxLines int // pieces longer than this are split
	MaxBytes int // hard cap on a chunk's code, to stay inside Jev's token limit

	// ParseTimeout gives up on a runaway tree-sitter parse and uses the
	// plain splitter instead; 0 means no limit. It is a safety net, not a
	// speed limit: whether a parse beats the clock depends on how busy the
	// machine is, so a file near it could split differently each search.
	ParseTimeout time.Duration
}

// Default gives most functions a chunk of their own, so results point at
// one function rather than a run of them; only pieces under 10 lines merge.
// Small chunks cost requests: each Jev call is ~235 ms (the client runs
// many in parallel) and carries a fixed ~300-token overhead, so going much
// smaller mostly buys overhead. 48 KB is ~17.5K tokens at bytes ÷ 2.75,
// well inside Jev's 32K state limit.
//
// Nearly every file parses in a few milliseconds and the slowest take
// seconds: on a 15K-file repo the worst was a 44 KB Swift file, 5.7 s
// alone and 8.8 s with every core parsing. A 250 ms limit once sent 23
// files there to the plain splitter on one search and not the next,
// including central ones like Effect.ts (146 ms alone), so chunks and
// cache keys changed between identical searches. The parser's own work
// limits scale with file size and don't depend on the clock.
var Default = Options{MinLines: 10, MaxLines: 300, MaxBytes: 48 << 10, ParseTimeout: 30 * time.Second}

// Version names how Split cuts files. Bump it whenever the same file could
// split differently, so chunk layouts cached by search.Load aren't reused.
const Version = 1

const maxSignature = 100

// rank orders how well a piece's header describes a chunk.
type rank int

const (
	rankSkip  rank = iota // comments, imports, package lines
	rankOther             // top-level statements, plain blocks
	rankDef               // functions, classes, types
)

// piece is a run of whole lines, or a byte segment of one long line.
type piece struct {
	start, end int // 0-based rows, inclusive
	lo, hi     int // byte segment of row start; hi == 0 means whole rows
	sig        string
	rank       rank
}

func (p piece) segment() bool { return p.hi > 0 }

// Split cuts src into chunks. filename picks the tree-sitter grammar; files
// without one, or that fail to parse, use a plain indentation splitter.
func Split(filename string, src []byte, opts Options) []Chunk {
	text := string(src)
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	s := splitter{text: text, lines: lines, starts: make([]int, len(lines)), opts: opts}
	for r := 1; r < len(lines); r++ {
		s.starts[r] = s.starts[r-1] + len(lines[r-1]) + 1
	}
	pieces, ok := s.treePieces(filename, src)
	if !ok {
		pieces = s.fallbackPieces()
	}
	return s.emit(s.merge(s.limit(pieces)))
}

type splitter struct {
	text   string
	lines  []string // of text
	starts []int    // byte offset of each line in the source
	opts   Options
}

func (s splitter) rowBytes(start, end int) int {
	n := 0
	for r := start; r <= end; r++ {
		n += len(s.lines[r]) + 1
	}
	return n - 1
}

func (s splitter) tooBig(start, end int) bool {
	return end-start+1 > s.opts.MaxLines || s.rowBytes(start, end) > s.opts.MaxBytes
}

// candidate is a row where a new piece may begin.
type candidate struct {
	row     int
	sig     string
	rank    rank
	comment bool
}

// treePieces splits at syntax nodes.
func (s splitter) treePieces(filename string, src []byte) (pieces []piece, ok bool) {
	bt, ok := parse(filename, src, s.opts.ParseTimeout)
	if !ok {
		return nil, false
	}
	defer bt.Release()
	cands := s.candidates(bt, bt.RootNode(), "")
	cands = attachComments(cands)

	for i, c := range cands {
		start := c.row
		if i == 0 {
			start = 0
		}
		end := len(s.lines) - 1
		if i+1 < len(cands) {
			end = cands[i+1].row - 1
		}
		pieces = append(pieces, piece{start: start, end: end, sig: c.sig, rank: c.rank})
	}
	return pieces, len(pieces) > 0
}

// candidates lists where pieces may start inside node. Children too big to
// be one piece are opened up, and the child's header row joins its first
// inner piece so a method's signature stays with its first lines.
func (s splitter) candidates(bt *gotreesitter.BoundTree, node *gotreesitter.Node, enclosing string) []candidate {
	var out []candidate
	for _, c := range namedChildren(node) {
		typ := bt.NodeType(c)
		start, end := int(c.StartPoint().Row), int(c.EndPoint().Row)
		if end >= len(s.lines) {
			end = len(s.lines) - 1
		}
		k := kind(typ, enclosing != "")
		sig := enclosing
		if k == rankDef || enclosing == "" {
			sig = s.signature(start, end)
		}
		if k != rankDef && enclosing != "" {
			k = rankOther
		}
		if s.tooBig(start, end) && slices.ContainsFunc(c.Children(), (*gotreesitter.Node).IsNamed) {
			// Non-definitions on the header row (names, parameters) are
			// part of the header, which joins the first body candidate.
			var body []candidate
			for _, ic := range s.candidates(bt, c, sig) {
				if ic.row > start || ic.rank == rankDef {
					body = append(body, ic)
				}
			}
			if len(body) > 0 {
				body[0].row = start
				if k == rankDef {
					body[0].sig, body[0].rank = sig, k
				}
				out = append(out, body...)
				continue
			}
		}
		out = append(out, candidate{row: start, sig: sig, rank: k, comment: strings.Contains(typ, "comment")})
	}
	return firstPerRow(out)
}

// namedChildren lists node's named children. NamedChild(i) and
// NamedChildCount count from the first child on every call, so looping over
// them is quadratic: a 3 MB file holding one long literal took 7 minutes.
func namedChildren(node *gotreesitter.Node) []*gotreesitter.Node {
	var out []*gotreesitter.Node
	for _, c := range node.Children() {
		if c.IsNamed() {
			out = append(out, c)
		}
	}
	return out
}

// firstPerRow keeps the first candidate on each row, e.g. a function rather
// than the name and parameters that share its header line.
func firstPerRow(cands []candidate) []candidate {
	out := cands[:0]
	for _, c := range cands {
		if n := len(out); n > 0 && c.row <= out[n-1].row {
			continue
		}
		out = append(out, c)
	}
	return out
}

// attachComments moves each run of comments onto the piece that follows it,
// so doc comments stay with what they document.
func attachComments(cands []candidate) []candidate {
	for i := len(cands) - 2; i >= 0; i-- {
		if cands[i].comment && !cands[i+1].comment {
			cands[i+1].row = cands[i].row
			cands = append(cands[:i], cands[i+1:]...)
		}
	}
	return cands
}

// Node type words that mark a definition. Top-level declarations of any
// kind name their chunk; inside a definition only nested definitions do,
// so a local variable never replaces the enclosing function's name.
var (
	skipWords   = []string{"comment", "import", "package", "include", "using", "use_declaration", "shebang", "extern_crate"}
	defWords    = []string{"function", "method", "class", "struct", "interface", "enum", "trait", "impl", "module", "namespace", "def", "constructor", "protocol", "object", "macro", "record"}
	topDefWords = []string{"type", "declaration"}
)

func kind(typ string, nested bool) rank {
	if containsAny(typ, skipWords) {
		return rankSkip
	}
	if strings.HasSuffix(typ, "identifier") {
		return rankOther
	}
	if containsAny(typ, defWords) || (!nested && containsAny(typ, topDefWords)) {
		return rankDef
	}
	return rankOther
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// fallbackPieces starts a new piece at each unindented line after a blank
// line or a closing line such as "}" or "end".
func (s splitter) fallbackPieces() []piece {
	var pieces []piece
	start := 0
	for r := 1; r < len(s.lines); r++ {
		line, prev := s.lines[r], strings.TrimSpace(s.lines[r-1])
		if strings.TrimSpace(line) == "" || indented(line) || closer(line) {
			continue
		}
		if prev == "" || (!indented(s.lines[r-1]) && closer(prev)) {
			pieces = append(pieces, piece{start: start, end: r - 1, sig: s.signature(start, r-1), rank: rankOther})
			start = r
		}
	}
	return append(pieces, piece{start: start, end: len(s.lines) - 1, sig: s.signature(start, len(s.lines)-1), rank: rankOther})
}

func indented(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

func closer(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "}") || strings.HasPrefix(t, ")") || strings.HasPrefix(t, "]") || t == "end" || strings.HasPrefix(t, "end ")
}

// signature is the first line in rows that isn't blank, a comment or a
// decorator, trimmed for display.
func (s splitter) signature(start, end int) string {
	first := ""
	for r := start; r <= end && r < len(s.lines); r++ {
		t := strings.TrimSpace(s.lines[r])
		if t == "" {
			continue
		}
		if first == "" {
			first = t
		}
		if !looksLikeComment(t) {
			first = t
			break
		}
	}
	first = strings.TrimSpace(strings.TrimSuffix(first, "{"))
	if utf8.RuneCountInString(first) > maxSignature {
		first = string([]rune(first)[:maxSignature-1]) + "…"
	}
	return first
}

func looksLikeComment(t string) bool {
	for _, p := range []string{"//", "#", "/*", "*", "--", ";", "@", "<!--", `"""`} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// limit splits pieces that are still over the line or byte limits.
func (s splitter) limit(pieces []piece) []piece {
	var out []piece
	for _, p := range pieces {
		if !s.tooBig(p.start, p.end) {
			out = append(out, p)
			continue
		}
		start := p.start
		for r := p.start; r <= p.end; r++ {
			if len(s.lines[r]) > s.opts.MaxBytes {
				if r > start {
					out = append(out, s.sub(p, start, r-1))
				}
				out = append(out, s.segments(p, r)...)
				start = r + 1
				continue
			}
			if r > start && s.tooBig(start, r) {
				cut := r - 1
				// Prefer ending on a blank line in the back half.
				for b := r - 1; b > start+(r-start)/2; b-- {
					if strings.TrimSpace(s.lines[b]) == "" {
						cut = b
						break
					}
				}
				out = append(out, s.sub(p, start, cut))
				start = cut + 1
			}
		}
		if start <= p.end {
			out = append(out, s.sub(p, start, p.end))
		}
	}
	return out
}

func (s splitter) sub(p piece, start, end int) piece {
	return piece{start: start, end: end, sig: p.sig, rank: p.rank}
}

// segments cuts one over-long line at rune boundaries.
func (s splitter) segments(p piece, row int) []piece {
	var out []piece
	line := s.lines[row]
	for lo := 0; lo < len(line); {
		hi := min(lo+s.opts.MaxBytes, len(line))
		for hi < len(line) && hi > lo && !utf8.RuneStart(line[hi]) {
			hi--
		}
		out = append(out, piece{start: row, end: row, lo: lo, hi: hi, sig: p.sig, rank: p.rank})
		lo = hi
	}
	return out
}

// merge joins short neighbouring pieces while the result stays in limits.
// A run of imports, package lines and comments always joins what follows
// it: on its own it would cost a Jev request and never match a question.
func (s splitter) merge(pieces []piece) [][]piece {
	var groups [][]piece
	fits := func(g []piece, p piece) bool {
		return !g[0].segment() && !p.segment() && !s.tooBig(g[0].start, p.end)
	}
	short := func(g []piece) bool {
		return g[len(g)-1].end-g[0].start+1 < s.opts.MinLines || !slices.ContainsFunc(g, func(p piece) bool { return p.rank != rankSkip })
	}
	for _, p := range pieces {
		if n := len(groups); n > 0 && short(groups[n-1]) && fits(groups[n-1], p) {
			groups[n-1] = append(groups[n-1], p)
			continue
		}
		groups = append(groups, []piece{p})
	}
	// A short tail joins the group before it.
	if n := len(groups); n > 1 && short(groups[n-1]) && fits(groups[n-2], groups[n-1][len(groups[n-1])-1]) {
		groups[n-2] = append(groups[n-2], groups[n-1]...)
		groups = groups[:n-1]
	}
	return groups
}

func (s splitter) emit(groups [][]piece) []Chunk {
	var out []Chunk
	for _, g := range groups {
		sig := groupSignature(g)
		if g[0].segment() {
			p := g[0]
			out = append(out, Chunk{Start: p.start + 1, End: p.start + 1, Signature: sig, Code: s.lines[p.start][p.lo:p.hi], Offset: s.starts[p.start] + p.lo})
			continue
		}
		start, end := g[0].start, g[len(g)-1].end
		for start <= end && strings.TrimSpace(s.lines[start]) == "" {
			start++
		}
		for end >= start && strings.TrimSpace(s.lines[end]) == "" {
			end--
		}
		if start > end {
			continue
		}
		out = append(out, Chunk{
			Start:     start + 1,
			End:       end + 1,
			Signature: sig,
			Code:      s.text[s.starts[start] : s.starts[end]+len(s.lines[end])],
			Offset:    s.starts[start],
		})
	}
	return out
}

// groupSignature names a chunk by its first best-ranked piece and counts the
// other definitions it holds.
func groupSignature(g []piece) string {
	best := 0
	for i, p := range g {
		if p.rank > g[best].rank {
			best = i
		}
	}
	more := 0
	seen := map[string]bool{g[best].sig: true}
	for i, p := range g {
		if i != best && p.rank == rankDef && !seen[p.sig] {
			seen[p.sig] = true
			more++
		}
	}
	if more > 0 {
		return g[best].sig + " +" + strconv.Itoa(more) + " more"
	}
	return g[best].sig
}
