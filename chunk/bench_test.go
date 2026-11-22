package chunk

import (
	"fmt"
	"strings"
	"testing"
)

// docCommentedGo is a Go file of n small functions, each with a doc comment.
func docCommentedGo(n int) []byte {
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := range n {
		fmt.Fprintf(&b, "// F%d does a thing.\nfunc F%d(a int) int {\n", i, i)
		for j := range i % 30 {
			fmt.Fprintf(&b, "\ta += %d\n", j)
		}
		b.WriteString("\treturn a\n}\n\n")
	}
	return []byte(b.String())
}

func BenchmarkSplit(b *testing.B) {
	cases := []struct {
		name, file string
		src        []byte
	}{
		{"go-200funcs", "big.go", docCommentedGo(200)},
		{"go-5000funcs", "big.go", docCommentedGo(5000)},
		{"fallback-200funcs", "big.unknownext", docCommentedGo(200)},
		// One unbroken block: every line is a split candidate for limit.
		{"fallback-oneblock", "big.unknownext", []byte(strings.Repeat("x := compute(a, b, c) + offset\n", 50000))},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.src)))
			b.ReportAllocs()
			for b.Loop() {
				Split(c.file, c.src, Default)
			}
		})
	}
}
