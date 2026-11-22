// Package chunk splits source files into function- or block-sized pieces,
// each sent to Jev as one state.
package chunk

import (
	"bytes"
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
//
// 2: Go files go through go/parser, so valid Go that tree-sitter rejects
// gets real chunks instead of the plain splitter's.
const Version = 2

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
// without one, or that fail to parse, use a plain indentation splitter. Go
// files are read with a fast Go scanner/parser instead, which finds the same
// pieces many times faster (see goCandidates).
func Split(filename string, src []byte, opts Options) []Chunk {
	chunks, _ := split(filename, src, opts, true)
	return chunks
}

// route is how split found a file's pieces.
type route int

const (
	routePlain  route = iota // the plain splitter
	routeTree                // tree-sitter, over the whole file
	routeGo                  // go fast path alone
	routeGoTree              // go fast path, and tree-sitter to open up big declarations
)

// split is Split. With goParser false it parses Go files with tree-sitter
// like any other, which tests compare against.
func split(filename string, src []byte, opts Options, goParser bool) ([]Chunk, route) {
	if len(src) == 0 {
		return nil, routePlain
	}
	text := string(src)
	starts := lineStarts(src)
	nLines := len(starts) - 1
	if nLines == 0 {
		return nil, routePlain
	}
	s := splitter{text: text, starts: starts, nLines: nLines, opts: opts}
	pieces, via := s.treePieces(filename, src, goParser)
	if via == routePlain {
		pieces = s.fallbackPieces()
	}
	limited := s.limit(pieces)
	return s.emit(limited, s.merge(limited)), via
}

// lineStarts returns the byte offset of every line in src plus a sentinel at
// the end so line r is src[starts[r] : starts[r+1]-1]. A trailing newline does
// not start an extra empty line, matching strings.Split + dropping trailing "".
func lineStarts(src []byte) []int {
	if len(src) == 0 {
		return nil
	}
	// Estimate line count to avoid reallocations.
	est := len(src)/32 + 2
	starts := make([]int, 1, est)
	starts[0] = 0
	pos := 0
	for {
		idx := bytes.IndexByte(src[pos:], '\n')
		if idx < 0 {
			starts = append(starts, len(src)+1)
			break
		}
		next := pos + idx + 1
		if next == len(src) {
			// Trailing newline: line r ends at len(src)-1, sentinel is len(src).
			starts = append(starts, len(src))
			break
		}
		starts = append(starts, next)
		pos = next
	}
	return starts
}

type splitter struct {
	text   string
	lines  []string // optional: only populated by direct unit tests constructing splitter{lines: ...}
	starts []int    // byte offset of each line in the source, plus sentinel at index nLines
	nLines int
	opts   Options
}

func (s splitter) numLines() int {
	if s.lines != nil {
		return len(s.lines)
	}
	return s.nLines
}

func (s splitter) line(r int) string {
	if s.lines != nil {
		return s.lines[r]
	}
	return s.text[s.starts[r] : s.starts[r+1]-1]
}

func (s splitter) lineLen(r int) int {
	if s.lines != nil {
		return len(s.lines[r])
	}
	return s.starts[r+1] - 1 - s.starts[r]
}

// rowBytes is the length of rows start..end joined by newlines.
func (s splitter) rowBytes(start, end int) int {
	if end < start {
		return 0
	}
	if s.lines != nil {
		return s.starts[end] + len(s.lines[end]) - s.starts[start]
	}
	return s.starts[end+1] - 1 - s.starts[start]
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

// treePieces splits at syntax nodes and says how it found them; routePlain
// means it couldn't.
func (s splitter) treePieces(filename string, src []byte, goParser bool) ([]piece, route) {
	var cands []candidate
	via := routePlain
	if goParser && isGo(filename) {
		cands, via = s.goCandidates(filename, src)
	}
	if via == routePlain {
		bt, ok := parse(filename, src, s.opts.ParseTimeout)
		if !ok {
			return nil, routePlain
		}
		defer bt.Release()
		cands, via = s.candidates(bt, bt.RootNode(), ""), routeTree
	}
	cands = attachComments(cands)

	nLines := s.numLines()
	pieces := make([]piece, 0, len(cands))
	for i, c := range cands {
		start := c.row
		if i == 0 {
			start = 0
		}
		end := nLines - 1
		if i+1 < len(cands) {
			end = cands[i+1].row - 1
		}
		pieces = append(pieces, piece{start: start, end: end, sig: c.sig, rank: c.rank})
	}
	if len(pieces) == 0 {
		return nil, routePlain
	}
	return pieces, via
}

// candidates lists where pieces may start inside node.
func (s splitter) candidates(bt *gotreesitter.BoundTree, node *gotreesitter.Node, enclosing string) []candidate {
	var out []candidate
	for _, c := range namedChildren(node) {
		out = append(out, s.nodeCandidates(bt, c, enclosing)...)
	}
	return firstPerRow(out)
}

// nodeCandidates lists where pieces may start in node. A node too big to be
// one piece is opened up, and its header row joins its first inner piece so
// a method's signature stays with its first lines.
func (s splitter) nodeCandidates(bt *gotreesitter.BoundTree, node *gotreesitter.Node, enclosing string) []candidate {
	typ := bt.NodeType(node)
	start, end := int(node.StartPoint().Row), int(node.EndPoint().Row)
	if nLines := s.numLines(); end >= nLines {
		end = nLines - 1
	}
	c := s.leaf(typ, start, end, enclosing)
	if s.tooBig(start, end) && slices.ContainsFunc(node.Children(), (*gotreesitter.Node).IsNamed) {
		// Non-definitions on the header row (names, parameters) are part
		// of the header, which joins the first body candidate.
		var body []candidate
		for _, ic := range s.candidates(bt, node, c.sig) {
			if ic.row > start || ic.rank == rankDef {
				body = append(body, ic)
			}
		}
		if len(body) > 0 {
			body[0].row = start
			if c.rank == rankDef {
				body[0].sig, body[0].rank = c.sig, c.rank
			}
			return body
		}
	}
	return []candidate{c}
}

// leaf is the candidate for a node of type typ on rows start..end that
// isn't opened up. Top-level nodes of any kind name their piece; a nested
// one keeps the enclosing name unless it's a definition.
func (s splitter) leaf(typ string, start, end int, enclosing string) candidate {
	c := candidate{row: start, sig: enclosing, rank: kind(typ, enclosing != ""), comment: strings.Contains(typ, "comment")}
	if c.rank == rankDef || enclosing == "" {
		c.sig = s.signature(start, end)
	}
	if c.rank != rankDef && enclosing != "" {
		c.rank = rankOther
	}
	return c
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
// so doc comments stay with what they document. A run with nothing after it
// stays as it is.
func attachComments(cands []candidate) []candidate {
	out := cands[:0]
	run := -1 // index in out of the current run's first comment
	for _, c := range cands {
		if c.comment {
			if run < 0 {
				run = len(out)
			}
			out = append(out, c)
			continue
		}
		if run >= 0 {
			c.row = out[run].row
			out = out[:run]
			run = -1
		}
		out = append(out, c)
	}
	return out
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
	// Fast path for the exact Go node types emitted by goNodes.
	switch typ {
	case "function_declaration", "method_declaration":
		return rankDef
	case "comment", "import_declaration", "package_clause":
		return rankSkip
	case "const_declaration", "type_declaration", "var_declaration":
		if !nested {
			return rankDef
		}
		return rankOther
	}
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
	nLines := s.numLines()
	for r := 1; r < nLines; r++ {
		line := s.line(r)
		if isBlank(line) || indented(line) || closer(line) {
			continue
		}
		prevLine := s.line(r - 1)
		if prev := strings.TrimSpace(prevLine); prev == "" || (!indented(prevLine) && closer(prev)) {
			pieces = append(pieces, piece{start: start, end: r - 1, sig: s.signature(start, r-1), rank: rankOther})
			start = r
		}
	}
	return append(pieces, piece{start: start, end: nLines - 1, sig: s.signature(start, nLines-1), rank: rankOther})
}

func isBlank(line string) bool {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f' {
			continue
		}
		if c < utf8.RuneSelf {
			return false
		}
		return strings.TrimSpace(line[i:]) == ""
	}
	return true
}

func indented(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

func closer(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "}") || strings.HasPrefix(t, ")") || strings.HasPrefix(t, "]") || t == "end" || strings.HasPrefix(t, "end ")
}

// signature is the first line in rows that isn't blank, a comment or a
// decorator, trimmed for display.
func (s splitter) signature(start, end int) string {
	first := ""
	nLines := s.numLines()
	for r := start; r <= end && r < nLines; r++ {
		t := strings.TrimSpace(s.line(r))
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
	n, cut := 0, 0
	for i := range first {
		if n == maxSignature-1 {
			cut = i
		}
		if n == maxSignature {
			first = first[:cut] + "…"
			break
		}
		n++
	}
	return first
}

func looksLikeComment(t string) bool {
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '#', '*', ';', '@':
		return true
	case '/':
		return len(t) >= 2 && (t[1] == '/' || t[1] == '*')
	case '-':
		return len(t) >= 2 && t[1] == '-'
	case '<':
		return strings.HasPrefix(t, "<!--")
	case '"':
		return strings.HasPrefix(t, `"""`)
	}
	return false
}

// limit splits pieces that are still over the line or byte limits.
func (s splitter) limit(pieces []piece) []piece {
	out := make([]piece, 0, len(pieces))
	for _, p := range pieces {
		if !s.tooBig(p.start, p.end) {
			out = append(out, p)
			continue
		}
		start := p.start
		for r := p.start; r <= p.end; r++ {
			if s.lineLen(r) > s.opts.MaxBytes {
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
					if isBlank(s.line(b)) {
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
	line := s.line(row)
	for lo := 0; lo < len(line); {
		hi := min(lo+s.opts.MaxBytes, len(line))
		for hi < len(line) && hi > lo && !utf8.RuneStart(line[hi]) {
			hi--
		}
		if hi <= lo {
			// MaxBytes is smaller than this rune: take the whole rune
			// rather than loop forever on an empty segment.
			_, n := utf8.DecodeRuneInString(line[lo:])
			hi = lo + n
		}
		out = append(out, piece{start: row, end: row, lo: lo, hi: hi, sig: p.sig, rank: p.rank})
		lo = hi
	}
	return out
}

// pieceGroup is a contiguous slice pieces[lo:hi] merged into one chunk.
type pieceGroup struct {
	lo, hi int
}

// merge joins short neighbouring pieces while the result stays in limits.
// A run of imports, package lines and comments always joins what follows
// it: on its own it would cost a Jev request and never match a question.
func (s splitter) merge(pieces []piece) []pieceGroup {
	groups := make([]pieceGroup, 0, len(pieces))
	fits := func(g pieceGroup, p piece) bool {
		return !pieces[g.lo].segment() && !p.segment() && !s.tooBig(pieces[g.lo].start, p.end)
	}
	short := func(g pieceGroup) bool {
		if pieces[g.hi-1].end-pieces[g.lo].start+1 < s.opts.MinLines {
			return true
		}
		for i := g.lo; i < g.hi; i++ {
			if pieces[i].rank != rankSkip {
				return false
			}
		}
		return true
	}
	for i, p := range pieces {
		if n := len(groups); n > 0 && short(groups[n-1]) && fits(groups[n-1], p) {
			groups[n-1].hi = i + 1
			continue
		}
		groups = append(groups, pieceGroup{lo: i, hi: i + 1})
	}
	// A short tail joins the group before it.
	if n := len(groups); n > 1 && short(groups[n-1]) && fits(groups[n-2], pieces[groups[n-1].hi-1]) {
		groups[n-2].hi = groups[n-1].hi
		groups = groups[:n-1]
	}
	return groups
}

func (s splitter) emit(pieces []piece, groups []pieceGroup) []Chunk {
	out := make([]Chunk, 0, len(groups))
	for _, gr := range groups {
		g := pieces[gr.lo:gr.hi]
		sig := groupSignature(g)
		if g[0].segment() {
			p := g[0]
			out = append(out, Chunk{Start: p.start + 1, End: p.start + 1, Signature: sig, Code: s.line(p.start)[p.lo:p.hi], Offset: s.starts[p.start] + p.lo})
			continue
		}
		start, end := g[0].start, g[len(g)-1].end
		for start <= end && isBlank(s.line(start)) {
			start++
		}
		for end >= start && isBlank(s.line(end)) {
			end--
		}
		if start > end {
			continue
		}
		out = append(out, Chunk{
			Start:     start + 1,
			End:       end + 1,
			Signature: sig,
			Code:      s.text[s.starts[start] : s.starts[end]+s.lineLen(end)],
			Offset:    s.starts[start],
		})
	}
	return out
}

// groupSignature names a chunk by its first best-ranked piece and counts the
// other definitions it holds.
func groupSignature(g []piece) string {
	if len(g) == 1 {
		return g[0].sig
	}
	best := 0
	for i, p := range g {
		if p.rank > g[best].rank {
			best = i
		}
	}
	more := 0
	bestSig := g[best].sig
	if len(g) <= 8 {
		for i, p := range g {
			if i == best || p.rank != rankDef || p.sig == bestSig {
				continue
			}
			dup := false
			for j := 0; j < i; j++ {
				if j != best && g[j].rank == rankDef && g[j].sig == p.sig {
					dup = true
					break
				}
			}
			if !dup {
				more++
			}
		}
	} else {
		var seen map[string]bool
		for i, p := range g {
			if i == best || p.rank != rankDef || p.sig == bestSig {
				continue
			}
			if seen == nil {
				seen = make(map[string]bool, 8)
			}
			if !seen[p.sig] {
				seen[p.sig] = true
				more++
			}
		}
	}
	if more > 0 {
		return bestSig + " +" + strconv.Itoa(more) + " more"
	}
	return bestSig
}
