package chunk

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type span struct {
	Start, End int
	Signature  string
}

func spans(chunks []Chunk) []span {
	var out []span
	for _, c := range chunks {
		out = append(out, span{c.Start, c.End, c.Signature})
	}
	return out
}

func assertSpans(t *testing.T, got []Chunk, want []span) {
	t.Helper()
	g := spans(got)
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Fatalf("got  %v\nwant %v", g, want)
	}
}

// assertCovers checks every non-blank line lands in exactly one chunk and
// chunk code is exactly the source lines it claims.
func assertCovers(t *testing.T, src string, chunks []Chunk, opts Options) {
	t.Helper()
	lines := strings.Split(src, "\n")
	seen := make([]int, len(lines)+1)
	for _, c := range chunks {
		if c.Start < 1 || c.End < c.Start || c.End > len(lines) {
			t.Fatalf("bad range %d-%d", c.Start, c.End)
		}
		if c.End-c.Start+1 > opts.MaxLines {
			t.Errorf("chunk %d-%d is over %d lines", c.Start, c.End, opts.MaxLines)
		}
		if len(c.Code) > opts.MaxBytes {
			t.Errorf("chunk %d-%d is %d bytes, over %d", c.Start, c.End, len(c.Code), opts.MaxBytes)
		}
		if !strings.Contains(strings.Join(lines[c.Start-1:c.End], "\n"), c.Code) {
			t.Errorf("chunk %d-%d code is not its source lines", c.Start, c.End)
		}
		if c.Offset < 0 || c.Offset+len(c.Code) > len(src) || src[c.Offset:c.Offset+len(c.Code)] != c.Code {
			t.Errorf("chunk %d-%d code is not the source at its offset %d", c.Start, c.End, c.Offset)
		}
		for i := c.Start; i <= c.End; i++ {
			seen[i]++
		}
	}
	for i, l := range lines {
		if strings.TrimSpace(l) != "" && seen[i+1] == 0 {
			t.Errorf("line %d %q is in no chunk", i+1, l)
		}
	}
}

const goSrc = `package billing

import "errors"

// RetryFailedPayment tries the charge again.
func RetryFailedPayment(invoice string, attempt int) error {
	if attempt > 3 {
		return errors.New("too many attempts")
	}
	return nil
}

type Invoice struct {
	ID string
}

func (i Invoice) Total() int {
	return 0
}
`

func TestGoSplitsAtDeclarationsWithDocComments(t *testing.T) {
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20}
	got := Split("retry.go", []byte(goSrc), opts)
	assertSpans(t, got, []span{
		{1, 11, "func RetryFailedPayment(invoice string, attempt int) error"},
		{13, 15, "type Invoice struct"},
		{17, 19, "func (i Invoice) Total() int"},
	})
	assertCovers(t, goSrc, got, opts)
}

func TestTinyPiecesMergeAndKeepTheFirstDefinitionAsSignature(t *testing.T) {
	opts := Options{MinLines: 6, MaxLines: 300, MaxBytes: 1 << 20}
	got := Split("retry.go", []byte(goSrc), opts)
	assertSpans(t, got, []span{
		{1, 11, "func RetryFailedPayment(invoice string, attempt int) error"},
		{13, 19, "type Invoice struct +1 more"},
	})
	assertCovers(t, goSrc, got, opts)
}

func TestLargeClassSplitsAtMethods(t *testing.T) {
	var b strings.Builder
	b.WriteString("class PaymentWorker:\n")
	for _, name := range []string{"charge", "refund", "retry"} {
		fmt.Fprintf(&b, "    def %s(self, job):\n", name)
		for i := 0; i < 8; i++ {
			fmt.Fprintf(&b, "        step_%d = job.next()\n", i)
		}
		b.WriteString("\n")
	}
	src := b.String()
	opts := Options{MinLines: 1, MaxLines: 12, MaxBytes: 1 << 20}
	got := Split("worker.py", []byte(src), opts)
	assertSpans(t, got, []span{
		{1, 10, "class PaymentWorker:"},
		{12, 20, "def refund(self, job):"},
		{22, 30, "def retry(self, job):"},
	})
	assertCovers(t, src, got, opts)
}

func TestHugeFunctionBodyKeepsItsSignature(t *testing.T) {
	var b strings.Builder
	b.WriteString("func Huge() {\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "\tx%d := %d\n", i, i)
	}
	b.WriteString("}\n")
	src := b.String()
	opts := Options{MinLines: 1, MaxLines: 15, MaxBytes: 1 << 20}
	got := Split("huge.go", []byte("package p\n"+src), opts)
	if len(got) < 4 {
		t.Fatalf("expected the body to be split, got %v", spans(got))
	}
	for _, c := range got[1:] {
		if c.Signature != "func Huge()" {
			t.Errorf("chunk %d-%d signature %q, want the enclosing function", c.Start, c.End, c.Signature)
		}
	}
	assertCovers(t, "package p\n"+src, got, opts)
}

func TestUnknownLanguageFallsBackToBlocks(t *testing.T) {
	src := `# Deploy notes

step one: build
  with flags

step two: ship
  carefully
`
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20}
	got := Split("notes.unknownext", []byte(src), opts)
	assertSpans(t, got, []span{
		{1, 1, "# Deploy notes"},
		{3, 4, "step one: build"},
		{6, 7, "step two: ship"},
	})
	assertCovers(t, src, got, opts)
}

func TestBrokenSourceFallsBack(t *testing.T) {
	src := "package p\n\nfunc {{{ broken\n\nfunc ok() {}\n"
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20}
	got := Split("broken.go", []byte(src), opts)
	if len(got) == 0 {
		t.Fatal("broken files still produce chunks")
	}
	assertCovers(t, src, got, opts)
}

func TestByteLimitSplitsLongLines(t *testing.T) {
	src := "var x = '" + strings.Repeat("a", 5000) + "';\nvar y = 1;\n"
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1000}
	got := Split("min.js", []byte(src), opts)
	if len(got) < 5 {
		t.Fatalf("expected the long line to be cut, got %v", spans(got))
	}
	assertCovers(t, src, got, opts)
}

func TestDefaultsRespectLimitsOnARealisticFile(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "// F%d does a thing.\nfunc F%d(a int) int {\n", i, i)
		for j := 0; j < i%30; j++ {
			fmt.Fprintf(&b, "\ta += %d\n", j)
		}
		b.WriteString("\treturn a\n}\n\n")
	}
	src := b.String()
	got := Split("big.go", []byte(src), Default)
	assertCovers(t, src, got, Default)
	for _, c := range got[:len(got)-1] {
		if n := c.End - c.Start + 1; n < Default.MinLines/2 {
			t.Errorf("chunk %d-%d has only %d lines", c.Start, c.End, n)
		}
	}
}

func TestEmptyAndBlankFiles(t *testing.T) {
	for _, src := range []string{"", "\n\n  \n"} {
		if got := Split("a.go", []byte(src), Default); len(got) != 0 {
			t.Errorf("%q: got %v", src, spans(got))
		}
	}
}

func TestDefaultsGiveDozenLineFunctionsTheirOwnChunk(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\n")
	names := []string{"Alpha", "Beta", "Gamma", "Delta"}
	for _, n := range names {
		fmt.Fprintf(&b, "func %s() int {\n", n)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(&b, "\tx%d := %d\n", i, i)
		}
		b.WriteString("\treturn 0\n}\n\n")
	}
	got := Split("p.go", []byte(b.String()), Default)
	var sigs []string
	for _, c := range got {
		sigs = append(sigs, c.Signature)
	}
	want := []string{"func Alpha() int", "func Beta() int", "func Gamma() int", "func Delta() int"}
	if fmt.Sprint(sigs) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", sigs, want)
	}
}

func TestImportsNeverMakeAChunkOfTheirOwn(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "import { thing%d } from \"./mod%d\";\n", i, i)
	}
	b.WriteString("\nexport function chargeCard(card: Card): Receipt {\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "  const step%d = card.next();\n", i)
	}
	b.WriteString("  return receipt;\n}\n")
	src := b.String()
	got := Split("pay.ts", []byte(src), Default)
	assertSpans(t, got, []span{{1, 41, "export function chargeCard(card: Card): Receipt"}})
	assertCovers(t, src, got, Default)
}

func TestAParseThatRunsOutOfTimeFallsBack(t *testing.T) {
	// Python, as Go files only reach tree-sitter for big declarations.
	var b strings.Builder
	b.WriteString("import os\n\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "def f%d(a):\n    if a > %d:\n        return a\n    return %d\n\n", i, i, i)
	}
	src := []byte(b.String())
	opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: 1 << 20}
	// Only the parser knows "import os" is an import, so only the parser
	// merges it into the first function.
	fallback := Split("big.unknownext", src, opts)[0]
	if parsed := Split("big.py", src, opts)[0]; parsed.End == fallback.End {
		t.Fatal("test needs a file the parser and the fallback split differently")
	}
	opts.ParseTimeout = time.Nanosecond
	got := Split("big.py", src, opts)
	if got[0] != fallback {
		t.Fatalf("a timed-out parse should use the plain splitter: first chunk %v, want %v", spans(got[:1]), spans([]Chunk{fallback}))
	}
	assertCovers(t, string(src), got, opts)
}

func TestByteLimitBelowOneRuneStillCutsTheLine(t *testing.T) {
	// Each rune is 3 bytes, more than MaxBytes: every segment has to hold
	// one whole rune, or the cut would never move forward.
	src := "x = '" + strings.Repeat("日", 20) + "'\n"
	for _, maxBytes := range []int{0, 1, 2} {
		opts := Options{MinLines: 1, MaxLines: 300, MaxBytes: maxBytes}
		got := Split("notes.unknownext", []byte(src), opts)
		var joined strings.Builder
		for _, c := range got {
			if c.Start != 1 || c.End != 1 || c.Code == "" || len(c.Code) > max(maxBytes, len("日")) {
				t.Fatalf("MaxBytes %d: bad segment %d-%d %q", maxBytes, c.Start, c.End, c.Code)
			}
			if src[c.Offset:c.Offset+len(c.Code)] != c.Code {
				t.Fatalf("MaxBytes %d: segment %q is not the source at offset %d", maxBytes, c.Code, c.Offset)
			}
			joined.WriteString(c.Code)
		}
		if joined.String()+"\n" != src {
			t.Fatalf("MaxBytes %d: segments join to %q", maxBytes, joined.String())
		}
	}
}

func TestRowBytesIsTheJoinedLength(t *testing.T) {
	src := "package p\n\nfunc é() {}\n\t\n// end"
	lines := strings.Split(src, "\n")
	s := splitter{text: src, lines: lines, starts: make([]int, len(lines))}
	for r := 1; r < len(lines); r++ {
		s.starts[r] = s.starts[r-1] + len(lines[r-1]) + 1
	}
	for start := range lines {
		for end := start; end < len(lines); end++ {
			if got, want := s.rowBytes(start, end), len(strings.Join(lines[start:end+1], "\n")); got != want {
				t.Errorf("rowBytes(%d, %d) = %d, want %d", start, end, got, want)
			}
		}
	}
	if got := s.rowBytes(len(lines), len(lines)-1); got != 0 {
		t.Errorf("an empty range has %d bytes", got)
	}
}

func TestCommentRunsJoinWhatFollowsThem(t *testing.T) {
	c := func(row int) candidate { return candidate{row: row, comment: true} }
	d := func(row int) candidate { return candidate{row: row, sig: fmt.Sprint("def", row), rank: rankDef} }
	got := attachComments([]candidate{d(0), c(2), c(3), d(4), d(8), c(10), d(11), c(15), c(16)})
	want := []candidate{d(0), d(4), d(8), d(11), c(15), c(16)}
	want[1].row, want[3].row = 2, 10
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestSplitIsSafeForConcurrentUse(t *testing.T) {
	srcs := map[string]string{
		"retry.go": goSrc,
		// Over Default.MaxLines, so tree-sitter opens it up.
		"long.go": "package p\n\nfunc Long() {\n" + strings.Repeat("\tx := 1\n\t_ = x\n", 200) + "}\n",
		"pay.py":  "def pay():\n    return 1\n\n\ndef refund():\n    return 2\n",
	}
	want := map[string][]Chunk{}
	for name, src := range srcs {
		want[name] = Split(name, []byte(src), Default)
	}
	done := make(chan error)
	for i := range 8 {
		go func() {
			for name, src := range srcs {
				if got := Split(name, []byte(src), Default); fmt.Sprint(got) != fmt.Sprint(want[name]) {
					done <- fmt.Errorf("goroutine %d: %s split differently", i, name)
					return
				}
			}
			done <- nil
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
