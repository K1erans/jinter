//go:build jinter_cgo

package chunk

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Split works with every language routed to C tree-sitter. The two
// backends' trees differ in places, so the chunks may too; the test lists
// the files where they do.
func TestSplitWithCTreeSitter(t *testing.T) {
	files, _ := filepath.Glob("testdata/golden/*")
	same, diff := 0, []string{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		for _, opts := range goldenOptions {
			c := opts
			c.Parsers = "*=c"
			if !strings.Contains(c.String(), "Parsers:*=c") {
				t.Fatalf("options %s don't name their routing", c)
			}
			got := Split(name, src, c)
			for _, ch := range got {
				if ch.Offset < 0 || ch.Offset+len(ch.Code) > len(src) || string(src[ch.Offset:ch.Offset+len(ch.Code)]) != ch.Code {
					t.Fatalf("%s: chunk %d-%d isn't the source at its offset", name, ch.Start, ch.End)
				}
			}
			if slices.Equal(got, Split(name, src, opts)) {
				same++
			} else {
				diff = append(diff, name)
			}
		}
	}
	diff = slices.Compact(diff)
	t.Logf("%d file×options splits match gotreesitter's; these files differ: %v", same, diff)
}
